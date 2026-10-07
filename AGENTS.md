# AGENTS.md

Guidance for AI coding agents (Claude Code, Codex, Copilot, etc.) working in this repo.
Human contributors should find it useful too.

## What this is

`sal-pub` is an early-stage Go service that gives users of the `sal` Keycloak realm an
ActivityPub identity (actor, WebFinger, inbox/outbox). Users sign in through Keycloak
(which brokers Microsoft, GitHub, Google and GitLab logins), and each Keycloak subject
maps to one ActivityPub actor stored in Postgres.

The project is a **prototype**: a minimal ActivityPub server in `internal/` and `cmd/server/`,
backed by Postgres, with Keycloak OIDC login. It uses `go-ap/activitypub` for the vocabulary
and `go-fed/httpsig` for HTTP Signatures. User-facing docs are in `docs/activitypub-server.md`.

## Layout

| Path | Purpose |
|---|---|
| `cmd/server/main.go` | The server: reads config, opens Postgres, runs migrations, wires `activitypub` + `web` onto one mux. Listens on `:8000`. |
| `internal/activitypub/` | WebFinger, actor, inbox, outbox, followers/following and note handlers (`server.go`); signed delivery, remote actor/key fetching and inbox processing (`federation.go`). Also provisions an actor on first login (`EnsureActor`). |
| `internal/httpsig/` | Thin wrapper around `go-fed/httpsig`: `Sign`, `Verify` (plus required-header, Date-skew and Digest checks), RSA key generation and PEM parsing. |
| `internal/store/` | Postgres access (`database/sql` + `lib/pq`) for `actors`, `notes` and `followers`, plus `Migrate`. |
| `internal/web/` | Browser side: Keycloak login (state + PKCE), gorilla session cookie, home page, `POST /notes`, `POST /logout` (RP-initiated logout via Keycloak's end-session endpoint). |
| `pkg/util/` | Small shared helpers (`GetEnv`). |
| `migrations/` | Plain SQL migrations, numbered `NNN_name.sql`, embedded via `migrations.FS` and applied at startup (tracked in `schema_migrations`). |
| `docs/` | User-facing documentation. |
| `Dockerfile` | Multi-stage build of `cmd/server` into a distroless image (the `salpub` Compose service). |
| `docker/caddy/` | Reverse proxy at `SALPUB_HOSTNAME`: `/auth/*` → Keycloak, everything else → `salpub:8000`. |
| `docker/keycloak/` | Keycloak 26 image plus the `sal` realm import (`sal.json`). |
| `docker/postgres/` | Init scripts mounted into `/docker-entrypoint-initdb.d`; creates the `salpub` database on a fresh data directory. |

## Commands

```sh
# Build / check
go build ./...
go vet   ./...
gofmt -l .                      # should print nothing
go test  ./...

# Full stack: Caddy + sal-pub + Keycloak + Postgres, served at $SALPUB_HOSTNAME
cp .env.default .env            # first time only, then fill in KEYCLOAK_CLIENT_SECRET etc.
docker compose up -d --build
docker compose up -d --build salpub   # after changing Go code
docker compose logs -f salpub keycloak
docker compose exec postgres createdb -U keycloak salpub   # only if Postgres predates docker/postgres/

# Or run the server outside Docker (needs localhost:8000 SALPUB_HOSTNAME/REDIRECT_URL; see docs)
set -a && source .env && set +a && go run ./cmd/server
```

Keycloak admin is at `$SALPUB_HOSTNAME/auth`. Default bootstrap credentials are in `.env.default`.

## Configuration

All runtime config comes from environment variables, read through `util.GetEnv(key, fallback)`.
`.env.default` is the committed template and `.env` is the local, git-ignored copy. When you add
a variable, add it to `.env.default` with a safe placeholder value.

Variables the Go code reads: `LISTEN_ADDR`, `DATABASE_URL`, `KC_HOSTNAME`,
`KC_HTTP_RELATIVE_PATH`, `KEYCLOAK_REALM`, `KEYCLOAK_CLIENT_ID`, `KEYCLOAK_CLIENT_SECRET`,
`REDIRECT_URL`, `SESSION_SECRET`, `OIDC_INSECURE_SKIP_VERIFY`, `OIDC_DISCOVERY_URL`. See `docs/activitypub-server.md`
for defaults.

`docker/keycloak/sal.json` uses `${VAR}` placeholders, which Keycloak fills from its own
environment when it imports the realm. Identity-provider credentials come from `IDP_<ALIAS>_CLIENT_ID`
and `IDP_<ALIAS>_CLIENT_SECRET` (plus `IDP_MICROSOFT_TENANT_ID`). Don't put literal secrets in that
file, and don't name new variables with a `KC_` prefix, because Keycloak reserves it for its own options.
`--import-realm` skips a realm that already exists, so changes only take effect when Keycloak starts
with a fresh database.

## Conventions

- Go 1.25. Use only the standard library plus what's already in `go.mod`. Ask before adding a dependency.
- Run `gofmt` on every Go file you touch.
- Wrap errors with context: `fmt.Errorf("verb noun: %w", err)`. Return `store.ErrNotFound` for missing rows.
- HTTP handlers are methods on a struct that holds their dependencies (`activitypub.Server`,
  `web.App`), registered with Go 1.22+ method/wildcard patterns in a `Register(mux)` method.
- Build and parse ActivityStreams documents with `go-ap/activitypub` (imported as `vocab`) and
  serialize them with `marshal()` in `internal/activitypub`, which adds `@context`. Don't hand-build
  AP JSON with maps.
- ActivityPub responses use `Content-Type: application/activity+json`. WebFinger responses use `application/jrd+json`.
- Schema changes go in a new numbered file in `migrations/`. Never edit a migration that has already been applied.
- Prefer the standard library or an existing dependency over hand-written helpers.

## Known issues and gotchas

- **Secrets.** Real IdP client secrets live only in `.env` and in `t.json` (a full realm export).
  Both files are git-ignored. Don't copy secret values into code, docs or commit messages, and don't
  print them in output.
- **Inbox security.** Every inbox request must pass `httpsig.Verify`, and the activity's `actor`
  must equal the signing key's owner. Keep both checks for any new activity types.
- `OIDC_INSECURE_SKIP_VERIFY=true` disables TLS verification for Keycloak calls only (needed with
  Caddy's local CA). Never apply it to the federation HTTP client or `http.DefaultTransport`.
- If `SESSION_SECRET` is empty, a random key is used and sessions end on restart.
- Federation needs `BASE_URL` reachable over public HTTPS.
- Caddy routes `/auth/*` to Keycloak, so app routes (including the OAuth callback,
  `/oauth/callback`) must stay out of `/auth`.
- In Docker, the public hostname isn't reachable from inside `salpub`. OIDC discovery goes through
  `OIDC_DISCOVERY_URL`, which relies on Keycloak's `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true` and a
  full-URL `KC_HOSTNAME` that includes `/auth`. Don't remove either setting.
- No delivery retry queue, and no caching of remote keys.
- `actors.private_key_pem` is stored in plaintext.

## Working agreements for agents

- Keep changes small and scoped to the task. Don't reformat or restructure unrelated code.
- Check that your change passes the `go build` / `go vet` / `go test` commands above, and report
  the results honestly.
- Don't run `docker compose down -v` or drop database tables
  without asking first.
- Commit only when asked. Branch off `main` first.

## Code Style
- Use testify for writing succinct tests; avoid just the standard library and if statements with t.Error().
- If a function would only be called from one other function and it is short, try to just condense it into the function that calls it.
- Any function with functionality that is non trivial should be documented with a succinct comment of what it does.
- Don't create functions that are very small and only used in a single place.
- Prefer to not use table oriented tests unless there is significant test duplication; make each test case a separate test function so it is easier to debug and isolate failures.
- All tests should be unit tests without external fetches to remote servers. All local filesystem operations should be mocked or written to tmp files.

## Boundaries
Ask First:
  - Large cross-package refactors.
  - If some functionality would be very complex, duplicative, and better handled by an underlying library like json-gold or goRDFlib, explicitly call it out; don't try to create a custom fix here with lots of specialized code
  - If the AGENTS.md file should be updated to reflect new changes, explicitly call it out

## Development
- go test ./... should pass after every new feature
- For any major new user facing features, add associated documentation in the /docs folder
- Use ripgrep via rg instead of grep if it is present to speed up searching through the codebase