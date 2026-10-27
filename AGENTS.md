# AGENTS.md

Guidance for AI coding agents (Claude Code, Codex, Copilot, etc.) working in this repo.
Human contributors should find it useful too.

## What this is

`sal-pub` is a ActivityPub implementation. 
The project is in `internal/` and `cmd/server/`, backed by Postgres, with 
Keycloak OIDC login. It uses `go-ap/activitypub` for the vocabulary and 
`go-fed/httpsig` for HTTP Signatures. User-facing docs are in 
`docs/activitypub-server.md`.

In addition to the Go ActivityPub server implementation, the repository hosts necessary
components to deploy a full ActivityPub implementation. This includes a terraform deployment 
and docker infrastructure deploy. This includes a name Keycloak realm through which an
ActivityPub identity (actor, WebFinger, inbox/outbox) can be created. Users sign in through Keycloak
(which brokers Microsoft, GitHub, Google and GitLab logins).

## Layout

| Path | Purpose |
|---|---|
| `cmd/server/main.go` | The server: reads config, opens Postgres, runs migrations, wires `activitypub` + `web` onto one mux. Listens on `:8000`. |
| `internal/activitypub/` | WebFinger, actor, inbox, outbox, followers/following and note handlers, note addressing (`ParseAddresses`) (`server.go`); signed delivery to addressed recipients, WebFinger handle resolution, remote actor/key fetching and inbox processing (`federation.go`); follow state as collections in the object store, with `Follow`/`Unfollow` and actor caching (`follows.go`); local notes as objects and collections, with publish (`notes.go`); update and delete of any of a user's objects, including the federation side effects for notes, plus HTTP CRUD for generic objects (`objects.go`); client-to-server `POST` to the outbox, which performs `Create`, `Update`, `Delete`, `Follow`, `Like`, `Announce` and `Undo` of the last three through `PostOutbox` (`outbox.go`); likes, shares and replies of local objects as `<object>/likes`, `/shares` and `/replies` collections, with sending, receiving and serving them (`interactions.go`). Also provisions an actor on first login (`EnsureActor`). |
| `internal/httpsig/` | Wrapper around `go-fed/httpsig`: `Sign`, `Verify` (plus required-header, Date-skew and Digest checks), RSA key generation and PEM parsing. |
| `internal/store/` | Postgres access (`database/sql` + `lib/pq`) for `actors` and `inbox_notes`, plus `Migrate` (`store.go`). Generic CRUD for any ActivityPub object and collection membership, behind the `Objects` and `Collections` interfaces, plus `CollectionObjects` to list a collection's stored objects (`objects.go`, tables `objects` and `collection_items`). |
| `internal/web/` | Browser side: Keycloak login (state + PKCE), gorilla session cookie, home page, `POST /notes` and `/follow`/`/unfollow` as thin wrappers that post activities through `PostOutbox`, `POST /users/{username}/outbox` (which the Edit note dialog's Save and Delete buttons post `Update`/`Delete` activities to from `static/app.js`), `POST /logout` (RP-initiated logout via Keycloak's end-session endpoint). HTML lives in `templates/` (`home.html` plus `partials/`), CSS/JS in `static/` (served at `/static/`); both are embedded with `//go:embed`, so rebuild after editing them. |
| `pkg/util/` | Shared helpers: `GetEnv`, `WithScheme`, and the HTTP helpers `ServerError` and `LogRequests`. |
| `migrations/` | Plain SQL migrations, numbered `NNN_name.sql`, embedded via `migrations.FS` and applied at startup (tracked in `schema_migrations`). |
| `docs/` | User-facing documentation. |
| `Dockerfile` | Multi-stage build of `cmd/server` into a distroless image (the `salpub` Compose service). |
| `docker/caddy/` | Reverse proxy at `SALPUB_HOSTNAME`: `/auth/*` and `/.well-known/openid-configuration` → Keycloak, everything else → `salpub:8000`. |
| `docker/keycloak/` | Keycloak image plus the `sal` realm import (`sal.json`). |
| `docker/postgres/` | Init scripts mounted into `/docker-entrypoint-initdb.d`; creates the `salpub` database on a fresh data directory. |
| `deploy/gcp/` | Terraform/OpenTofu for GCP: GKE Autopilot running the same three images (Caddy as the public entry point), Cloud SQL Postgres, Artifact Registry. `build-images.sh` builds linux/amd64 images. See its README. |

## Commands

```sh
# Build / check (all must pass after every change)
go build ./...
go vet   ./...
gofmt -l .                      # should print nothing
go test  ./...

# Store tests need a throwaway Postgres database; without the variable they're skipped.
docker compose exec postgres createdb -U keycloak salpub_test   # once
SALPUB_TEST_DATABASE_URL='postgres://keycloak:keycloak@localhost:5432/salpub_test?sslmode=disable' go test ./internal/...

# Full stack: Caddy + sal-pub + Keycloak + Postgres, served at $SALPUB_HOSTNAME
cp .env.default .env            # first time only, then fill in KEYCLOAK_CLIENT_SECRET etc.
docker compose up -d --build
docker compose up -d --build salpub   # after changing Go code
docker compose up -d --build caddy    # after changing the Caddyfile (it's baked into the image)
docker compose logs -f salpub keycloak
docker compose exec postgres createdb -U keycloak salpub   # only if Postgres predates docker/postgres/

# The server only runs under Compose: it reaches Keycloak at http://keycloak:8080.
```

Keycloak admin is at `$SALPUB_HOSTNAME/auth`. Default bootstrap credentials are in `.env.default`.

## Configuration

All runtime config comes from environment variables, read through `util.GetEnv(key, fallback)`.
`.env.default` is the committed template and `.env` is the local, git-ignored copy. When you add
a variable, add it to `.env.default` with a safe placeholder value.

Keep the number of variables small. Variables the Go code reads: `SALPUB_HOSTNAME`, `DATABASE_URL`,
`KEYCLOAK_CLIENT_SECRET`, `SESSION_SECRET`. Values that are fixed by the Docker setup are constants,
not variables: the listen address, the `/auth` path, the `sal` realm, the `sal-pub` client ID and the
callback path (`cmd/server/main.go`), and Keycloak's own settings (`docker/keycloak/Dockerfile`).
`docker-compose.yml` passes each service only the variables it needs; Keycloak gets `KC_HOSTNAME`
as `${SALPUB_HOSTNAME}/auth`. See `docs/activitypub-server.md` for details.

`docker/keycloak/sal.json` uses `${VAR}` placeholders, which Keycloak fills from its own
environment when it imports the realm. Identity-provider credentials come from `IDP_<ALIAS>_CLIENT_ID`
and `IDP_<ALIAS>_CLIENT_SECRET` (plus `IDP_MICROSOFT_TENANT_ID`). Don't put literal secrets in that
file, and don't name new variables with a `KC_` prefix, because Keycloak reserves it for its own options.
`--import-realm` skips a realm that already exists, so changes only take effect when Keycloak starts
with a fresh database.

## Conventions

- Go 1.25. Prefer the standard library or what's already in `go.mod`. Ask before adding a dependency.
- Run `gofmt` on every Go file you touch.
- Wrap errors with context: `fmt.Errorf("verb noun: %w", err)`. Return `store.ErrNotFound` for missing rows.
- HTTP handlers are methods on a struct that holds their dependencies (`activitypub.Server`,
  `web.App`), registered with Go 1.22+ method/wildcard patterns in a `Register(mux)` method.
  Report handler errors with `util.ServerError`.
- Build and parse ActivityStreams documents with `go-ap/activitypub` (imported as `vocab`) and
  serialize them with `marshal()` in `internal/activitypub`, which adds `@context`. Don't hand-build
  AP JSON with maps.
- ActivityPub responses use `Content-Type: application/activity+json`. WebFinger responses use `application/jrd+json`.
- Schema changes go in a new numbered file in `migrations/` (the next is `002_...`). Never edit a
  migration that has already been applied. `001_init.sql` is the whole schema as of 2026-10-14
  and assumes a clean database: databases created before then must be recreated, not migrated.
- Major user-facing features get documentation in `docs/`.
- Page markup goes in `internal/web/templates/`, not Go strings. Remote content must go through
  `plainText`, never `noteHTML`.

## Code style

- Write tests with testify. Avoid just the standard library and if statements with t.Error().
- One test function per case, so failures are easy to isolate. Use table-driven tests only when
  there's significant duplication.
- Write unit tests: no fetches to remote servers, and filesystem access is mocked or uses temp files.
  The exception is tests that need Postgres (`internal/store`, the object CRUD tests in
  `internal/activitypub`): they run against `SALPUB_TEST_DATABASE_URL` and skip without it.
- Don't create very small functions that are only called from one place; inline them into the caller.
- Any function with functionality that is non trivial should be documented with a succinct comment of what it does.

## ActivityPub Specifics

- **Inbox security.** Every inbox request must pass `httpsig.Verify`, and the activity's `actor`
  must equal the signing key's owner.
- **Remote HTML.** `inbox_notes.content` is unsanitized HTML from other servers. Never render it as
  `template.HTML`; the home page shows it through `plainText`. Rendering it as HTML needs a real
  sanitizer (e.g. bluemonday), which is a new dependency.
- **Follow state** is in `objects`/`collection_items` (see `follows.go`); there are no separate
  follower/following tables. An `Accept` only counts if it matches a pending `Follow` we sent to
  the signer (by follow id).
- **Local notes** are `Note` objects in `objects` with ids `<base>/notes/{id}`, listed in
  `<actor>#notes` (all), and `<actor>/outbox` and `<base>/notes` (public only); see `notes.go`.
- **Likes, shares and replies** of local objects are collections named `<object>/likes`, `/shares`
  (actor URIs, served as counts only) and `/replies` (public reply ids); see `interactions.go`. A
  local actor's own `Like`/`Announce` is stored with an id derived from its object (`reactionID`).
- **Note visibility.** Fetches of the outbox and `GET /notes/{id}` aren't authenticated, so they must
  only return notes addressed to `store.PublicAddress`. Keep that filter on any new note endpoint.

## Deployment specifics

- **Recipient fetching (SSRF).** Delivery fetches every address a signed-in user puts in To/CC, so a
  user can make the server request arbitrary URLs, including internal hosts. Not yet mitigated.
- **Keycloak from the container.** Inside `salpub`, `localhost` is the container itself, so the
  server fetches OIDC discovery from `http://keycloak:8080` and checks tokens against the public
  issuer. This relies on `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true` (internal token/JWKS URLs) and a
  full-URL `KC_HOSTNAME` including `/auth`. Don't remove either. Never disable TLS verification on
  the federation HTTP client or `http.DefaultTransport`.
- Caddy routes `/auth/*` to Keycloak, so app routes (including the OAuth callback,
  `/oauth/callback`) must stay out of `/auth`.
- Federation needs `SALPUB_HOSTNAME` reachable over public HTTPS.

## Working agreements for agents

- Keep changes small and scoped to the task. Don't reformat or restructure unrelated code.
- Check that your change passes the build/vet/gofmt/test commands above, and report the results honestly.
- Don't run `docker compose down -v` or drop database tables.
- Commit only when asked. Branch off `main` first.
- Ask first before:
  - large cross-package refactors;
  - writing complex, specialized code for something an existing library (e.g. json-gold, goRDFlib)
    would handle better. Call it out instead.
- Say so when a change means AGENTS.md should be updated.
- Real IdP client secrets live only in `.env`. Don't copy secret values into code, docs or commit messages, and don't print them in output.
