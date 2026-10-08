-- One ActivityPub actor per Keycloak subject. The private key never leaves
-- this table (it isn't part of any ActivityPub document).
CREATE TABLE actors (
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

-- Notes published by local actors. to/cc are absolute IRIs (the public
-- collection, followers collections, actors).
CREATE TABLE notes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id UUID NOT NULL REFERENCES actors (id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    published TIMESTAMPTZ NOT NULL DEFAULT now(),
    to_addresses TEXT[] NOT NULL DEFAULT '{}',
    cc_addresses TEXT[] NOT NULL DEFAULT '{}'
);

CREATE INDEX notes_actor_published_idx ON notes (actor_id, published DESC);

-- Notes delivered to a local actor's inbox in a Create, from actors they
-- follow or addressed to them directly. content is the sender's HTML, stored
-- as received; it must never be rendered as HTML without sanitizing.
CREATE TABLE inbox_notes (
    actor_id UUID NOT NULL REFERENCES actors (id) ON DELETE CASCADE,
    object_id TEXT NOT NULL,
    attributed_to TEXT NOT NULL,
    content TEXT NOT NULL,
    published TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (actor_id, object_id)
);

CREATE INDEX inbox_notes_actor_published_idx ON inbox_notes (actor_id, published DESC);

-- Generic ActivityPub object storage, keyed by the object's id (IRI). data is
-- the object's JSON as produced by go-ap (without @context). Holds objects
-- created through /objects, Follow activities we sent, and cached remote
-- actor documents.
CREATE TABLE objects (
    id TEXT PRIMARY KEY,
    data JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Membership of collections, keyed by IRI: <actor>/followers,
-- <actor>/following and <actor>#follows (see internal/activitypub/follows.go).
-- Items don't have to be in objects, and collections don't have to be objects.
CREATE TABLE collection_items (
    collection_id TEXT NOT NULL,
    item_id TEXT NOT NULL,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (collection_id, item_id)
);

CREATE INDEX collection_items_order_idx ON collection_items (collection_id, added_at DESC, item_id);
