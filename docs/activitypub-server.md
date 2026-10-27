# sal-pub ActivityPub server

`cmd/server` is a minimal ActivityPub server. Each person who signs in through
Keycloak gets an actor that other fediverse servers (Mastodon, etc.) can find
and follow. From the web page they can publish notes and choose who each note is
addressed to (by default: public, and delivered to their followers).

## Running it with Docker

```sh
cp .env.default .env          # first time; fill in KEYCLOAK_CLIENT_SECRET etc.
docker compose up -d --build  # Caddy + sal-pub + Keycloak + Postgres
docker compose logs -f salpub
```

Caddy serves everything at `SALPUB_HOSTNAME`:

- `/auth/*` goes to Keycloak.
- `/.well-known/openid-configuration` is proxied to the realm's discovery document
  (`/auth/realms/<realm>/.well-known/openid-configuration`). The issuer inside it is still the realm URL.
- Everything else goes to the `salpub` container, built from the root `Dockerfile`.

Open `SALPUB_HOSTNAME` and choose **Log in with Keycloak**. The first login creates your actor, and
the page then shows your handle (e.g. `@jane_doe@localhost`).

Compose starts `salpub` once Keycloak's healthcheck passes. `salpub` still exits if Postgres or
Keycloak isn't reachable, and Compose restarts it until it is. The Go binary and the Caddyfile are
both baked into their images, so rebuild after changing them:

```sh
docker compose up -d --build salpub   # after changing Go code
docker compose up -d --build caddy    # after changing docker/caddy/Caddyfile
```

**Reaching Keycloak from the container.** Browsers reach Keycloak through Caddy at
`SALPUB_HOSTNAME/auth`, but inside the `salpub` container `localhost` is the container itself. So
the server fetches OIDC discovery from the Compose service, `http://keycloak:8080/auth/realms/sal`,
and still checks tokens against the public issuer, `SALPUB_HOSTNAME/auth/realms/sal`. Keycloak's
`KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true` (set in `docker/keycloak/Dockerfile`) makes it give the
server internal token and key URLs too, so the server never calls Keycloak over HTTPS.

**Existing realms.** `--import-realm` skips a realm that already exists. If yours predates
these settings, open the `sal-pub` client in the Keycloak admin console and set:
- **Valid redirect URIs** to `SALPUB_HOSTNAME/oauth/callback` (e.g. `https://localhost/oauth/callback`)
- **Valid post logout redirect URIs** to `SALPUB_HOSTNAME/` (e.g. `https://localhost/`)

**Existing Postgres.** If Postgres was already running before `docker/postgres/01-salpub.sql`
existed, create the database once:

```sh
docker compose exec postgres createdb -U keycloak salpub
```

### Without Docker

Running `go run ./cmd/server` on the host isn't supported: the server reaches Keycloak at
`http://keycloak:8080`, which only resolves inside the Compose network.

## Configuration

Everything is configured in `.env` (copied from `.env.default`). The server itself reads:

| Variable | Default | Purpose |
|---|---|---|
| `SALPUB_HOSTNAME` | `http://localhost:8000` | Public URL. Actor IDs are built from it, so don't change it once real users exist. Use a full URL with scheme (e.g. `https://localhost`), because Keycloak's `KC_HOSTNAME` is derived from it as `SALPUB_HOSTNAME/auth`. |
| `KEYCLOAK_CLIENT_SECRET` | empty | Secret of the `sal-pub` OIDC client. Keycloak reads the same variable when it imports the realm. |
| `SESSION_SECRET` | random per start | Cookie signing key (`openssl rand -hex 32`). If empty, sessions end on restart. |
| `DATABASE_URL` | `postgres://keycloak:keycloak@localhost:5432/salpub?sslmode=disable` | Postgres DSN. Compose builds it from `KC_DB_USERNAME`/`KC_DB_PASSWORD`. Migrations in `migrations/` are applied at startup. |

`.env` also holds the Keycloak admin bootstrap credentials (`KC_BOOTSTRAP_ADMIN_*`), the Postgres
credentials (`KC_DB_USERNAME`, `KC_DB_PASSWORD`) and the identity-provider credentials (`IDP_*`).

Fixed values that used to be variables:

| Value | Where it's set |
|---|---|
| Listen address `:8000` | `cmd/server/main.go` |
| Keycloak path `/auth`, realm `sal`, client ID `sal-pub` | `cmd/server/main.go`, `docker/caddy/Caddyfile`, `docker/keycloak/sal.json`, `docker/keycloak/Dockerfile` |
| Redirect URL `SALPUB_HOSTNAME/oauth/callback` | `cmd/server/main.go`, `docker/keycloak/sal.json` |
| Other Keycloak settings (`KC_DB*`, `KC_HTTP_*`, `KC_PROXY_HEADERS`, `KC_HOSTNAME_BACKCHANNEL_DYNAMIC`) | `docker/keycloak/Dockerfile` |

## Endpoints

| Route | What it does |
|---|---|
| `GET /` | Home page: log in, see your handle and followers, publish notes. Everyone, logged in or not, sees the 20 most recent public posts from all users. |
| `GET /login`, `GET /oauth/callback` | Keycloak authorization-code flow with PKCE. |
| `POST /logout` | Clears the session, then ends the Keycloak SSO session through its end-session endpoint and returns to `SALPUB_HOSTNAME`. |
| `POST /notes` | Publishes a note. Form fields: `content`, `to`, `cc` (see [Addressing](#addressing)), and optionally `in_reply_to`. Session-authenticated and protected against cross-origin posts. |
| `GET /.well-known/webfinger?resource=acct:user@host` | WebFinger lookup. |
| `GET /users/{username}` | Actor (`Person`) with its public key. |
| `POST /follow`, `POST /unfollow` | Follow or unfollow the actor in form field `target` (see [Following](#following)). Session-authenticated and protected against cross-origin posts. |
| `POST /users/{username}/inbox` | Accepts `Follow`, `Undo{Follow}`, `Accept`/`Reject` of the user's own follows, `Create{Note}`, and `Like`, `Announce` and their `Undo` (see [Inbox](#inbox) and [Likes, shares and replies](#likes-shares-and-replies)). Requires a valid HTTP Signature. |
| `GET /users/{username}/outbox` | Latest 50 public `Create{Note}` activities. |
| `POST /users/{username}/outbox` | Performs an activity as you (client-to-server; see [Posting to the outbox](#posting-to-the-outbox)). Session-authenticated and protected against cross-origin posts. |
| `GET /users/{username}/followers`, `/following` | Collections. `following` lists accepted follows only. |
| `GET /notes` | Latest 50 public notes from all users, as an `OrderedCollection` of `Note` objects. |
| `GET /notes/{id}` | A single public note (404 for non-public notes). |
| `POST /objects`, `GET/PUT/DELETE /objects/{id}` | CRUD for generic objects (see [Objects](#objects)). |
| `GET /notes/{id}/likes`, `/shares`, `/replies` (and the same under `/objects/{id}`) | A note's or object's interactions (see [Likes, shares and replies](#likes-shares-and-replies)). |

## How it works

- **Identity.** On first login the Keycloak `sub` is mapped to a new actor. The username comes
  from `preferred_username`, lowercased and reduced to `[a-z0-9_]`. If that name is taken, a
  `_2`, `_3`, … suffix is added. The username never changes afterwards, but the display name
  follows Keycloak's `name` claim.
- **Keys.** Each actor gets a 2048-bit RSA key. It is used to sign outgoing requests, and remote
  servers verify those signatures with the public key published in the actor document.
- **Inbox.** The server verifies each incoming request with draft-cavage HTTP Signatures
  ([go-fed/httpsig](https://github.com/go-fed/httpsig)), which is what Mastodon uses. It
  requires `(request-target)`, `host`, `date` and `digest` to be signed, a `Date` within one
  hour of now, a `Digest` that matches the body, and a signing actor that matches the activity's
  `actor`. A `Follow` is accepted automatically, and the server replies with a signed `Accept`.
- **Addressing.** See [Addressing](#addressing) below.
- **Vocabulary.** ActivityStreams documents are built and parsed with
  [go-ap/activitypub](https://github.com/go-ap/activitypub).

## Addressing

The note form has **To** and **CC** fields, which become the note's ActivityPub `to` and `cc`.
Each field takes actor or collection URLs separated by spaces or commas, and `public` (or
`as:Public`) as shorthand for `https://www.w3.org/ns/activitystreams#Public`. A note needs at
least one recipient. The defaults reproduce the usual public post:

| Field | Default |
|---|---|
| To | `public` |
| CC | your followers collection, `<actor URL>/followers` |

Common variations:

- **Unlisted:** To = your followers collection, CC = `public`.
- **Followers only:** To = your followers collection, CC empty.
- **Direct:** To = one or more actor URLs, CC empty.

Delivery follows the addressing. If your followers collection is in To or CC, the note goes to
every follower's inbox. Every other address is fetched as an actor (with a signed request), and the
note goes to its `sharedInbox`, or its `inbox` if it has no shared inbox. Each inbox gets the note
once, and addresses that don't resolve to an actor with an inbox are logged and skipped.

The outbox and `GET /notes/{id}` aren't authenticated, so they only show notes addressed to
`public`. Your own home page lists all of your notes, with their addressing.

Notes are stored as `Note` objects in the generic object store (see [Objects](#objects)), with
ids `<SALPUB_HOSTNAME>/notes/{id}` and their `to`/`cc` as given. Each note is listed in its author's
private `<actor>#notes` collection, which the home page reads. Public notes are also listed in the
author's outbox (`<actor>/outbox`) and in the site-wide `<SALPUB_HOSTNAME>/notes` collection.

## Editing and deleting notes

Each of your notes, in the **Outbox** tab and in reply threads, has an **Edit** button. It opens the
Edit note dialog, which has **Save** and **Delete** buttons. They post an `Update` or `Delete` activity to your outbox (see [Posting to the outbox](#posting-to-the-outbox)), the same
as any other client would. Only a note's author can change it.

- **Save** replaces the note's text. Its addressing stays the same. The note gets an `updated`
  time, it shows as *edited*, and an `Update{Note}` is sent to the same inboxes as the original
  `Create`.
- **Delete** asks for confirmation, removes the note and sends a `Delete` with a `Tombstone` to the
  same inboxes. Remote servers decide for themselves whether to honor it.

Copies in local users' inboxes are updated or removed directly. Incoming `Update` and `Delete`
activities from remote servers are not handled yet, so remote notes in your inbox don't change.

## Following

The home page has a **Following** form. Enter a handle (`@user@example.org`) or an actor URL:

- **Remote actors.** The handle is resolved with WebFinger, the actor document is fetched (signed),
  and a signed `Follow` is sent to the actor's inbox. The follow shows as *pending* until the actor
  replies with an `Accept`, which must be signed by that actor and refer to the `Follow` we sent.
  A `Reject` removes it. If sending the `Follow` fails, nothing is recorded and the error is shown.
- **Local actors** (`@user@SALPUB_HOSTNAME` or a `/users/` URL on this server) are followed directly
  in the database and accepted immediately, since the server can't reach its own public URL from
  inside Docker.

**Unfollow** sends the actor an `Undo{Follow}` (or, for local actors, removes the follower row).
The follow is removed even if delivering the `Undo` fails.

Follow state is kept in the generic object store (see [Objects](#objects)):

- `<actor>/followers` and `<actor>/following` are collections of actor URIs (`following` holds
  accepted follows only); the matching endpoints read them directly.
- Each `Follow` a user sends is stored as an object and listed in a private `<actor>#follows`
  collection, which is how pending follows, `Accept`, `Reject` and unfollowing find it.
- Remote followers' and follow targets' actor documents are cached as objects, so delivery and
  `Undo` know their inboxes without fetching them again.

## Inbox

When a `Create{Note}` arrives at a user's inbox, the note is stored and shown in the **Inbox**
section of their home page (20 most recent). It is only stored if:

- the signature is valid and the activity's `actor` is the signer (as for every inbox request);
- the note is embedded in the activity (not just a link), and its `attributedTo` is the signer;
- the note's `id` is on the signer's host;
- the user follows the signer (accepted follow), **or** the user is addressed in the note's or
  activity's `to`/`cc`.

Anything else is logged and ignored (the server still answers `202`). A note already received is
not stored twice.

Notes published by local users are written straight into local recipients' inboxes (followers
and directly addressed users) instead of going over HTTP.

Remote content is HTML written by other servers. It is stored as received
(table `inbox_notes`), but the page only shows it as plain text, with
tags stripped and the text escaped, so links and formatting are lost.

## Objects

`internal/store` has generic storage for any ActivityPub object (table `objects`, keyed by `id`, data
as JSONB) and for collection membership (table `collection_items`, keyed by collection and item IRIs),
behind the `Objects` and `Collections` interfaces. Notes (see [Addressing](#addressing)) and following
(see [Following](#following)) use it.

Logged-in users can also manage plain objects (Note, Article, Image, … but not activities, actors or
collections) over HTTP, at `/objects/{id}`. These are separate from the notes published from the
home page, and changes to them aren't federated. Requests use the browser session; write requests must be same-origin.

| Request | Result |
|---|---|
| `POST /objects` with an object as JSON | `201`, `Location` header and the stored object. The server assigns the `id` (`/objects/<random>`), sets `attributedTo` to you and `published` to now. |
| `GET /objects/{id}` | The object. Objects not addressed to `public` (in `to`/`cc`) are 404 to everyone but their author. |
| `PUT /objects/{id}` with an object as JSON | Replaces it, keeping `id`, `attributedTo` and `published`, and setting `updated`. Author only (`403` otherwise). |
| `DELETE /objects/{id}` | `204`. Author only. |

These objects are stored, not federated: no `Create`, `Update` or `Delete` activity is sent.

## Posting to the outbox

Logged-in users can post activities as JSON to their own outbox, `POST /users/{username}/outbox`
(ActivityPub client-to-server). The request uses the browser session, and must be same-origin like
the forms. The server assigns ids and replies `201` with the activity as performed, and its id in
the `Location` header.

| Activity | What happens |
|---|---|
| `Create{Note}` | Published and delivered like a note from the home page. `to`/`cc` of the activity and the note are merged; at least one is required. |
| `Create{other object}` | Stored at `/objects/{id}` (see [Objects](#objects)), not federated. |
| `Update{object}` | Edits one of your notes (text only; its addressing stays the same) or replaces one of your objects. The object must be embedded, have its `id` and keep its `type`. |
| `Delete` | Deletes one of your notes or objects, by id. The returned activity's object is a `Tombstone`. |
| `Follow` | Follows the actor URL or `@user@host` handle in `object` (see [Following](#following)). |
| `Undo{Follow}` | Unfollows. The object is either the `Follow`'s id or a `Follow` whose `object` is the actor. |
| `Like`, `Announce` | Likes or shares (boosts) the object with the id in `object`, local or remote (see [Likes, shares and replies](#likes-shares-and-replies)). |
| `Undo{Like}`, `Undo{Announce}` | Takes it back. The object is the activity's id or an activity of that type with the same `object`. |

A bare object is treated as `Create{object}`. An `actor`, if given, must be you. Other activity types
are rejected with `400`. Changing another user's note or object gives `403`.

Note text is taken from `source` when its `mediaType` is `text/plain`, and otherwise from `content`
with the HTML removed. Either way it's limited to 5000 characters and rendered by the server, so
clients can't put their own HTML in a note.

The home page's forms (`POST /notes`, `/follow` and `/unfollow`) build these activities and go
through the same code. Its Edit note dialog's Save and Delete buttons post `Update` and `Delete`
activities to the outbox directly.

Public posts, your outbox and your inbox have Like and Reply buttons. Like posts a `Like`, or an
`Undo{Like}` when you already like the post. Reply opens the New note dialog with `in_reply_to` set
and the post's author added to CC. Local posts also show their like count and their public replies.
Replies are only shown nested under the post they reply to, as a thread: a public reply to a post in
the same list (the public posts, or your outbox) isn't listed on its own. Your replies to other
people's posts stay in your outbox, with an "in reply to" link. Logged-out visitors see the counts
and replies without the buttons.

## Likes, shares and replies

Every local note and object has three collections, linked from its `likes`, `shares` and `replies`
properties:

| Collection | Contents |
|---|---|
| `<object>/likes` | Actors who sent a `Like`. Served as a `Collection` with `totalItems` only, so who liked it isn't published. |
| `<object>/shares` | Actors who sent an `Announce`, served the same way. |
| `<object>/replies` | Ids of public notes with `inReplyTo` set to the object, as `items`. |

They're served under the same rules as the object: non-public objects only to their author (and
for `/notes/{id}/...`, not at all). Notes and objects created before this feature don't have the
properties, but their collections still work.

Local users like or share an object by posting `Like` or `Announce` to their outbox. Each user
reacts to an object at most once; posting again returns the earlier activity. For a local object,
the user is added to its collection. For a remote object, the server fetches it to find its author
and delivers the activity to them. An `Announce` is addressed to the public and the user's
followers as well, and only public objects can be shared. Remote servers' `Like`, `Announce` and
`Undo` of them for local objects are counted from the inbox.

A reply is a `Create{Note}` whose note has `inReplyTo`. A local parent must exist and be visible to
you. The reply isn't addressed to the parent's author automatically: put them in `cc` so they
receive it. Public replies to local objects, from local or remote users, are listed in the parent's
`replies`. Deleting a local reply removes it from there.

## Federating with real servers

Remote servers need to reach `SALPUB_HOSTNAME` over public HTTPS, and most refuse plain-HTTP actors.
For a quick test, run a tunnel (for example `cloudflared` or `ngrok`) in front of port 8000.
Set `SALPUB_HOSTNAME` to the tunnel URL, and add `<tunnel URL>/oauth/callback` as a redirect URI on the client in
Keycloak. Then search for `@you@your-tunnel-host` from a Mastodon account.

## Limitations

- Deliveries are sent once, with no retry queue.
- Remote keys aren't cached, so every inbox request fetches the sender's actor.
- The inbox ignores `Update` and `Delete`, so edited or deleted remote posts stay as first
  received, and deleted remote replies stay in `replies`. `Announce` only counts shares of local
  objects; boosted posts aren't shown.
- The home page has no Share button, and doesn't show share counts.
- `Like` and `Announce` of a remote object fetch whatever URL the user gives (SSRF, as below).
- Remote posts are shown as plain text; there is no HTML sanitizer yet.
- Notes are only accepted at each user's own inbox; there is no shared inbox.
- Like note recipients, follow targets are fetched from whatever a signed-in user enters (SSRF).
- Private keys are stored in plaintext in Postgres.
- Recipients must be full actor URLs; `@user@host` handles aren't resolved through WebFinger yet.
- The server fetches whatever recipient URLs a signed-in user enters, so a signed-in user can make it
  request arbitrary URLs, including internal hosts (SSRF).
- Non-public notes can't be fetched by their recipients' servers, because fetches aren't
  authenticated (only the pushed copy arrives).
